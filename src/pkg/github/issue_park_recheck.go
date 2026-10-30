package github

// Park re-check: an issue parked for a person (stage/045-needs-decision) is
// parked on a premise — "#722 is in progress, agents leave it alone", "the
// precedent has not been set", "whether to adopt ruff and type checking".
// The park text is written once, usually by a contributor relay under its
// own account, and nothing re-read it when the work it named moved on. A
// maintainer then decides from a premise that no longer holds, and agents
// stay off work that is unblocked (projectbluefin/server#385,
// projectbluefin/utah#13, projectbluefin/website#926).
//
// RecheckParkedIssues re-reads each open parked issue's timeline. A change
// counts when, after the park began,
//
//   - a pull request or issue the park text names merged or closed, or
//   - a pull request that refers to the issue ("Refs #N", a closing keyword,
//     "part of", …) merged.
//
// A pull request either kind of reference names that closed unmerged may
// have been adopted: the hive's copy of it supersedes it and lives on
// (automerge.AdoptionPRBody). The re-check follows it there and lists the
// copy when it merged, or when it closed unmerged, then with the note the
// person who closed it left. projectbluefin/utah#13 waited on #722; the
// hive's copy #873 was closed by a maintainer saying #885 and #905 landed
// the intent, and #873 never mentions #13, so nothing on the issue said
// its question had been answered.
//
// A PR that only lists the issue in passing is not a change: a person's
// intake table once linked ~65 issues (see hard_suppress_reconcile.go).
//
// When something changed, the hive posts one note under its own marker,
// listing what changed, and edits it in place as more lands. It never
// un-parks: leaving 045 stays a maintainer's reply (labels-and-control-
// signals.md), and a park can rest on a maintainer's explicit request that
// no merged PR answers (website#926 cited one).

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// parkRecheckMarker opens the hive's one re-check note per parked issue.
const parkRecheckMarker = "<!-- hive:park-recheck -->"

// parkRelabelGap bridges a park label swapped for another park label (the
// needs-decision → stage/085 → stage/045 migrations removed one and added the
// next a second apart): a gap this short does not start a new park.
const parkRelabelGap = 15 * time.Minute

// parkTextWindow is how long before the park label the park text may have
// been posted: the relay posts its comment, then the hive applies the label.
const parkTextWindow = 15 * time.Minute

// parkRecheckMaxNamedLookups bounds the per-issue reads for items the park
// text names that do not cross-reference the issue.
const parkRecheckMaxNamedLookups = 10

// parkRecheckMaxCopyLookups bounds the closed pull requests per issue that
// are followed to the hive's copy: one timeline read each, plus the copy's
// issue and comments when the copy closed unmerged too.
const parkRecheckMaxCopyLookups = 3

// parkCopyNoteWindow is how far from the copy's close a person's comment may
// be and still read as the closing note. GitHub's "Close with comment" posts
// both in the same second (utah#873: comment 02:17:24Z, closed 02:17:25Z);
// an hour also covers a person explaining just before or after closing by
// hand, while review discussion from earlier days (a question on #873 a day
// and a half before it closed) stays out.
const parkCopyNoteWindow = time.Hour

// parkCopyQuoteMax is the most runes of a closing note the re-check quotes.
const parkCopyQuoteMax = 300

// ParkRecheckConfig scopes one pass.
type ParkRecheckConfig struct {
	Repos []string
}

// ParkRecheckResult reports one pass.
type ParkRecheckResult struct {
	Posted  int      `json:"posted"`
	Updated int      `json:"updated"`
	Skipped int      `json:"skipped"`
	Items   []string `json:"items,omitempty"`
}

// parkRef is one item a park depends on, as the timeline reports it now.
type parkRef struct {
	repo     string // owner/repo
	number   int
	isPR     bool
	state    string
	mergedAt time.Time
	closedAt time.Time
	body     string
}

// parkChange is one thing that moved after the park began.
type parkChange struct {
	ref   string // "#409", or "owner/repo#N" outside the issue's repo
	verb  string // "merged", "was closed without merging", "was closed"
	at    time.Time
	named bool // the park text names it; else it refers to the issue
	// copyOf is set when ref is the hive's adoption copy of that PR.
	copyOf string
	// by, quote and link are the closing note a person left when the copy
	// closed unmerged, if any.
	by, quote, link string
}

// RecheckParkedIssues posts or refreshes the re-check note on every open
// issue parked at the configured needs-decision label whose park premise
// moved. See the file comment.
func (c *Client) RecheckParkedIssues(ctx context.Context, cfg ParkRecheckConfig) (*ParkRecheckResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	result := &ParkRecheckResult{}
	label := c.needsDecisionLabel()
	var firstErr error
	for _, repoRef := range cfg.Repos {
		owner, repo := c.splitRepo(repoRef)
		if owner == "" || repo == "" {
			continue
		}
		issues, err := c.listOpenIssuesWithLabel(ctx, owner, repo, label)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() {
				continue
			}
			if err := c.recheckParkedIssue(ctx, owner, repo, issue, label, result); err != nil {
				if c.logger != nil {
					c.logger.Warn("park re-check skipped issue", "repo", repo, "issue", issue.GetNumber(), "error", err)
				}
				result.Skipped++
			}
		}
	}
	return result, firstErr
}

func (c *Client) recheckParkedIssue(ctx context.Context, owner, repo string, issue *gh.Issue, label string, result *ParkRecheckResult) error {
	number := issue.GetNumber()
	events, err := c.listIssueTimelineAll(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	start := parkStartFrom(events, c.isDecisionParkLabel)
	if start.IsZero() {
		result.Skipped++
		return nil
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	refs := parkRefsFromTimeline(events)
	named := parkNamedRefs(parkText(issue, comments, start), owner+"/"+repo, number)
	lookups := 0
	for _, key := range named {
		if _, ok := refs[key]; ok || lookups >= parkRecheckMaxNamedLookups {
			continue
		}
		lookups++
		if ref, ok := c.readParkRef(ctx, key); ok {
			refs[key] = ref
		}
	}
	changes := parkChanges(refs, named, owner+"/"+repo, number, start)
	if copies := c.parkCopyChanges(ctx, refs, named, owner+"/"+repo, number, start, changes); len(copies) > 0 {
		changes = append(changes, copies...)
		sortParkChanges(changes)
	}
	if len(changes) == 0 {
		result.Skipped++
		return nil
	}
	note := parkRecheckNote(label, start, changes)
	item := c.WritebackItemForIssue(Issue{Number: number, Author: safeGetLogin(issue.GetUser()), Labels: issueLabelNames(issue.Labels), CreatedAt: issue.GetCreatedAt().Time})
	for _, cm := range comments {
		if cm == nil || !c.isHiveAccount(cm.GetUser()) || !strings.Contains(cm.GetBody(), parkRecheckMarker) {
			continue
		}
		if cm.GetBody() == note {
			result.Skipped++
			return nil
		}
		if d := c.WritebackAllowed(owner+"/"+repo, item, writeback.KindNeedsDecisionComment, note); !d.Allowed {
			result.Skipped++
			return nil
		}
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(note)}); err != nil { // writeback:gated
			return fmt.Errorf("editing park re-check note on %s/%s#%d: %w", owner, repo, number, err)
		}
		result.Updated++
		result.Items = append(result.Items, fmt.Sprintf("%s/%s#%d", owner, repo, number))
		return nil
	}
	if d := c.WritebackAllowed(owner+"/"+repo, item, writeback.KindNeedsDecisionComment, note); !d.Allowed {
		result.Skipped++
		return nil
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(note)}); err != nil { // writeback:gated
		return fmt.Errorf("posting park re-check note on %s/%s#%d: %w", owner, repo, number, err)
	}
	result.Posted++
	result.Items = append(result.Items, fmt.Sprintf("%s/%s#%d", owner, repo, number))
	if c.logger != nil {
		c.logger.Info("PARK-RECHECK", "repo", repo, "issue", number, "parked_since", start.UTC().Format(time.RFC3339), "changes", len(changes))
	}
	return nil
}

// isDecisionParkLabel reports whether label parks an issue for a person's
// decision. `blocked` parks on an outside blocker and is reconciled on its
// own (ReconcileBlockedIssues), so it does not start a decision park.
func (c *Client) isDecisionParkLabel(label string) bool {
	return !strings.EqualFold(strings.TrimSpace(label), issueBlockedLabel) && c.isParkingLabel(label)
}

func (c *Client) listIssueTimelineAll(ctx context.Context, owner, repo string, number int) ([]*gh.Timeline, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var out []*gh.Timeline
	for {
		events, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing timeline for %s/%s#%d: %w", owner, repo, number, err)
		}
		out = append(out, events...)
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// readParkRef reads one named item's current state; ok is false when it
// cannot be read (a typo, a deleted item, no access).
func (c *Client) readParkRef(ctx context.Context, key string) (parkRef, bool) {
	repoFull, n, ok := splitParkRefKey(key)
	if !ok {
		return parkRef{}, false
	}
	owner, repo := c.splitRepo(repoFull)
	issue, _, err := c.client.Issues.Get(ctx, owner, repo, n)
	if err != nil || issue == nil {
		return parkRef{}, false
	}
	ref := parkRef{repo: repoFull, number: n, isPR: issue.IsPullRequest(), state: issue.GetState(), closedAt: issue.GetClosedAt().Time, body: issue.GetBody()}
	if ref.isPR {
		ref.mergedAt = issue.GetPullRequestLinks().GetMergedAt().Time
	}
	return ref, true
}

// parkStartFrom returns when the issue's current park began: the first park
// label of the run that is still on the issue, bridging label swaps shorter
// than parkRelabelGap. Zero when the timeline shows no park in force.
func parkStartFrom(events []*gh.Timeline, isPark func(string) bool) time.Time {
	present := map[string]bool{}
	var start, emptySince time.Time
	for _, ev := range events {
		if ev == nil || ev.Label == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(ev.GetLabel().GetName()))
		if name == "" || !isPark(name) {
			continue
		}
		at := ev.GetCreatedAt().Time
		switch strings.ToLower(ev.GetEvent()) {
		case "labeled":
			if len(present) == 0 && (start.IsZero() || emptySince.IsZero() || at.Sub(emptySince) > parkRelabelGap) {
				start = at
			}
			present[name] = true
		case "unlabeled":
			delete(present, name)
			if len(present) == 0 {
				emptySince = at
			}
		}
	}
	if len(present) == 0 {
		return time.Time{}
	}
	return start
}

// parkText is the text the park rests on: the issue body when the issue was
// filed parked, and every comment posted in the window before the park
// label. The hive's own notices are not park text.
func parkText(issue *gh.Issue, comments []*gh.IssueComment, start time.Time) string {
	inWindow := func(at time.Time) bool {
		return !at.IsZero() && !at.After(start) && start.Sub(at) <= parkTextWindow
	}
	var parts []string
	if inWindow(issue.GetCreatedAt().Time) {
		parts = append(parts, issue.GetBody())
	}
	for _, cm := range comments {
		if cm == nil || !inWindow(cm.GetCreatedAt().Time) {
			continue
		}
		body := cm.GetBody()
		if strings.Contains(body, unparkNoticeMarker) || strings.Contains(body, parkRecheckMarker) {
			continue
		}
		parts = append(parts, body)
	}
	return strings.Join(parts, "\n")
}

// parkRefRE matches "#N" and "owner/repo#N" references.
var parkRefRE = regexp.MustCompile(`(?:^|[^\w/&#])(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#(\d+)\b`)

// parkNamedRefs returns the "owner/repo#N" keys the park text names, in
// order of first mention, without the issue itself.
func parkNamedRefs(text, issueRepo string, number int) []string {
	seen := map[string]bool{parkRefKey(issueRepo, number): true}
	var out []string
	for _, m := range parkRefRE.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil || n <= 0 {
			continue
		}
		repo := issueRepo
		if m[1] != "" {
			repo = m[1]
		}
		key := parkRefKey(repo, n)
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func parkRefKey(repo string, n int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(n)
}

func splitParkRefKey(key string) (string, int, bool) {
	i := strings.LastIndex(key, "#")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return key[:i], n, true
}

// parkRefsFromTimeline indexes the items that cross-reference the issue by
// key, with their current state as the timeline reports it.
func parkRefsFromTimeline(events []*gh.Timeline) map[string]parkRef {
	out := map[string]parkRef{}
	for _, ev := range events {
		if ev == nil || !strings.EqualFold(ev.GetEvent(), "cross-referenced") {
			continue
		}
		src := ev.GetSource()
		if src == nil || src.Issue == nil {
			continue
		}
		is := src.Issue
		repo := is.GetRepository().GetFullName()
		if repo == "" || is.GetNumber() <= 0 {
			continue
		}
		ref := parkRef{repo: repo, number: is.GetNumber(), isPR: is.IsPullRequest(), state: is.GetState(), closedAt: is.GetClosedAt().Time, body: is.GetBody()}
		if ref.isPR {
			ref.mergedAt = is.GetPullRequestLinks().GetMergedAt().Time
		}
		out[parkRefKey(repo, ref.number)] = ref
	}
	return out
}

// parkRelationWordRE matches the words that make a PR's mention of an issue
// a relation rather than a passing reference.
var parkRelationWordRE = regexp.MustCompile(`(?i)\b(?:refs?|references?|related\s+to|relates\s+to|part\s+of|addresses|implements|follow[- ]up\s+(?:to|of)|close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b`)

// parkRelationMaxRefs is the most references one relation clause may list
// and still count: a longer list is an intake table, not a relation.
const parkRelationMaxRefs = 10

// parkRefersTo reports whether a PR body refers to the issue with a relation
// word: the issue is among the references that follow the word up to the
// end of that sentence ("Refs #722 (…), #791 (…), #13."). A bare "#N"
// resolves to the PR's own repository, so "Refs other/repo#N" or a PR in
// another repository naming its own #N never reads as this issue.
func parkRefersTo(body, prRepo, issueRepo string, number int) bool {
	want := parkRefKey(issueRepo, number)
	for _, line := range strings.Split(body, "\n") {
		for _, loc := range parkRelationWordRE.FindAllStringIndex(line, -1) {
			clause := line[loc[1]:]
			if i := strings.Index(clause, ". "); i >= 0 {
				clause = clause[:i]
			}
			refs := parkNamedRefs(clause, prRepo, 0)
			if len(refs) > parkRelationMaxRefs {
				continue
			}
			for _, key := range refs {
				if key == want {
					return true
				}
			}
		}
	}
	return false
}

// parkChanges lists what moved after start, oldest first: named items that
// merged or closed, then (in time order with them) PRs referring to the
// issue that merged.
func parkChanges(refs map[string]parkRef, named []string, issueRepo string, number int, start time.Time) []parkChange {
	isNamed := map[string]bool{}
	for _, key := range named {
		isNamed[key] = true
	}
	render := func(ref parkRef) string { return parkRefDisplay(ref, issueRepo) }
	var out []parkChange
	for key, ref := range refs {
		switch {
		case isNamed[key]:
			switch {
			case ref.isPR && ref.mergedAt.After(start):
				out = append(out, parkChange{ref: render(ref), verb: "merged", at: ref.mergedAt, named: true})
			case ref.isPR && ref.mergedAt.IsZero() && strings.EqualFold(ref.state, "closed") && ref.closedAt.After(start):
				out = append(out, parkChange{ref: render(ref), verb: "was closed without merging", at: ref.closedAt, named: true})
			case !ref.isPR && strings.EqualFold(ref.state, "closed") && ref.closedAt.After(start):
				out = append(out, parkChange{ref: render(ref), verb: "was closed", at: ref.closedAt, named: true})
			}
		case ref.isPR && ref.mergedAt.After(start) && parkRefersTo(ref.body, ref.repo, issueRepo, number):
			out = append(out, parkChange{ref: render(ref), verb: "merged", at: ref.mergedAt})
		}
	}
	sortParkChanges(out)
	return out
}

// sortParkChanges orders changes oldest first, then by reference.
func sortParkChanges(out []parkChange) {
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.Before(out[j].at)
		}
		return out[i].ref < out[j].ref
	})
}

// parkRefDisplay renders ref as "#N", or "owner/repo#N" outside issueRepo.
func parkRefDisplay(ref parkRef, issueRepo string) string {
	if strings.EqualFold(ref.repo, issueRepo) {
		return "#" + strconv.Itoa(ref.number)
	}
	return ref.repo + "#" + strconv.Itoa(ref.number)
}

// parkCopyChanges follows each pull request the park depends on (named, or
// referring to the issue) that closed unmerged after start to the hive's
// adoption copy of it, and lists the copy when it merged or closed unmerged
// after start. A copy already listed (its merge refers to the issue itself)
// is not listed twice. An unmerged copy is listed when a person left a
// closing note on it, or when the park names the original, whose own close
// the note already lists: the copy's line says the adopted work closed too
// rather than living on. A read that fails counts as no copy, as in
// readParkRef.
func (c *Client) parkCopyChanges(ctx context.Context, refs map[string]parkRef, named []string, issueRepo string, number int, start time.Time, listed []parkChange) []parkChange {
	isNamed := map[string]bool{}
	for _, key := range named {
		isNamed[key] = true
	}
	var follow []string
	for key, ref := range refs {
		if _, _, adopted := AdoptedFrom(ref.body); adopted {
			continue // an adoption copy has no copy of its own to follow
		}
		if ref.isPR && ref.mergedAt.IsZero() && strings.EqualFold(ref.state, "closed") && ref.closedAt.After(start) &&
			(isNamed[key] || parkRefersTo(ref.body, ref.repo, issueRepo, number)) {
			follow = append(follow, key)
		}
	}
	// Oldest close first, so the cap keeps the same PRs on every pass.
	sort.Slice(follow, func(i, j int) bool {
		a, b := refs[follow[i]], refs[follow[j]]
		if !a.closedAt.Equal(b.closedAt) {
			return a.closedAt.Before(b.closedAt)
		}
		return follow[i] < follow[j]
	})
	if len(follow) > parkRecheckMaxCopyLookups {
		follow = follow[:parkRecheckMaxCopyLookups]
	}
	seen := map[string]bool{}
	for _, ch := range listed {
		seen[ch.ref] = true
	}
	var out []parkChange
	for _, key := range follow {
		orig := refs[key]
		owner, repo := c.splitRepo(orig.repo)
		events, err := c.listIssueTimelineAll(ctx, owner, repo, orig.number)
		if err != nil {
			continue
		}
		cp, ok := c.parkAdoptionCopy(events, orig)
		if !ok {
			continue
		}
		ch := parkChange{ref: parkRefDisplay(cp, issueRepo), copyOf: parkRefDisplay(orig, issueRepo), named: isNamed[key]}
		switch {
		case seen[ch.ref]:
			continue
		case cp.mergedAt.After(start):
			ch.verb, ch.at = "merged", cp.mergedAt
		case cp.mergedAt.IsZero() && strings.EqualFold(cp.state, "closed") && cp.closedAt.After(start):
			ch.verb, ch.at = "was closed without merging", cp.closedAt
			if note := c.parkCopyClosingNote(ctx, owner, repo, cp); note != nil {
				ch.by, ch.quote, ch.link = note.GetUser().GetLogin(), parkCopyQuote(note.GetBody()), note.GetHTMLURL()
			}
			if ch.quote == "" && !ch.named {
				continue
			}
		default:
			continue
		}
		seen[ch.ref] = true
		out = append(out, ch)
	}
	return out
}

// parkAdoptionCopy returns the hive's adoption copy of orig from orig's own
// timeline: the latest pull request in orig's repository that cross-
// references it, is authored by one of the hive's accounts, and whose body
// carries AdoptionPRBody's "Supersedes #N (…; original commits by @…
// preserved)" line for orig (AdoptedFrom). The latest, because once a copy
// closes unmerged the hive may adopt the original again.
func (c *Client) parkAdoptionCopy(events []*gh.Timeline, orig parkRef) (parkRef, bool) {
	var out parkRef
	found := false
	for _, ev := range events {
		if ev == nil || !strings.EqualFold(ev.GetEvent(), "cross-referenced") || ev.GetSource() == nil || ev.GetSource().Issue == nil {
			continue
		}
		is := ev.GetSource().Issue
		if !is.IsPullRequest() || is.GetNumber() == orig.number || !strings.EqualFold(is.GetRepository().GetFullName(), orig.repo) || !c.isHiveAccount(is.GetUser()) {
			continue
		}
		if n, _, ok := AdoptedFrom(is.GetBody()); !ok || n != orig.number {
			continue
		}
		out = parkRef{repo: orig.repo, number: is.GetNumber(), isPR: true, state: is.GetState(), closedAt: is.GetClosedAt().Time,
			mergedAt: is.GetPullRequestLinks().GetMergedAt().Time, body: is.GetBody()}
		found = true
	}
	return out, found
}

// parkCopyClosingNote returns the comment a person left closing the copy:
// the latest one within parkCopyNoteWindow of the close by the account that
// closed it, or by any person when the closer cannot be read. A copy the
// hive or another bot closed has no such note.
func (c *Client) parkCopyClosingNote(ctx context.Context, owner, repo string, cp parkRef) *gh.IssueComment {
	var closer *gh.User
	if issue, _, err := c.client.Issues.Get(ctx, owner, repo, cp.number); err == nil && issue != nil {
		closer = issue.ClosedBy
	}
	if closer != nil && !c.isHumanAuthor(closer) {
		return nil
	}
	comments, err := c.listIssueCommentsSince(ctx, owner, repo, cp.number, cp.closedAt.Add(-parkCopyNoteWindow))
	if err != nil {
		return nil
	}
	var note *gh.IssueComment
	for _, cm := range comments {
		if cm == nil || !c.isHumanAuthor(cm.GetUser()) {
			continue
		}
		if closer != nil && !strings.EqualFold(cm.GetUser().GetLogin(), closer.GetLogin()) {
			continue
		}
		at := cm.GetCreatedAt().Time
		if d := at.Sub(cp.closedAt); d < -parkCopyNoteWindow || d > parkCopyNoteWindow {
			continue
		}
		if note == nil || !at.Before(note.GetCreatedAt().Time) {
			note = cm
		}
	}
	if note == nil || parkCopyQuote(note.GetBody()) == "" {
		return nil
	}
	return note
}

// parkCopyQuote is the first paragraph of a closing note as one line, cut to
// parkCopyQuoteMax runes. The comment is a person's, not the hive's: HTML
// comments are removed so no hive marker is laundered into the note, and
// @mentions are defanged so quoting it notifies nobody, as in
// unparkOptionSummary.
func parkCopyQuote(body string) string {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if i := strings.Index(body, "\n\n"); i >= 0 {
		body = body[:i]
	}
	q := unparkHTMLCommentPattern.ReplaceAllString(strings.Join(strings.Fields(body), " "), " ")
	q = strings.Join(strings.Fields(defangMentions(q)), " ")
	if r := []rune(q); len(r) > parkCopyQuoteMax {
		q = strings.TrimSpace(string(r[:parkCopyQuoteMax-2])) + " …"
	}
	return q
}

// parkRecheckNote renders the note. It is deterministic for one set of
// changes, so an unchanged pass edits nothing.
func parkRecheckNote(label string, start time.Time, changes []parkChange) string {
	var b strings.Builder
	b.WriteString(parkRecheckMarker)
	fmt.Fprintf(&b, "\n**The reason this issue is parked may be out of date.** It has been parked for a maintainer (`%s`) since %s, and since then:\n\n", label, start.UTC().Format("2006-01-02"))
	noted := false
	for _, ch := range changes {
		if ch.copyOf != "" {
			fmt.Fprintf(&b, "- %s (the hive's copy of %s) %s on %s", ch.ref, ch.copyOf, ch.verb, ch.at.UTC().Format("2006-01-02"))
			if ch.quote != "" {
				noted = true
				fmt.Fprintf(&b, " by %s: “%s”", ch.by, ch.quote)
				if ch.link != "" {
					fmt.Fprintf(&b, " ([comment](%s))", ch.link)
				}
			}
			b.WriteString(".\n")
			continue
		}
		why := "it refers to this issue"
		if ch.named {
			why = "the park reason names it"
		}
		fmt.Fprintf(&b, "- %s %s on %s (%s).\n", ch.ref, ch.verb, ch.at.UTC().Format("2006-01-02"), why)
	}
	b.WriteString("\nThe park reason was written before these changes. Re-read it against the current state before deciding. This note does not un-park the issue: it stays with a maintainer, and Hive updates this note if more of the referenced work moves.")
	if noted {
		b.WriteString("\n\nIf the closing note quoted above settles the question, a maintainer can reply `/fixed` to close this issue as completed, or `/hive decision <your instructions>` to un-park it.")
	}
	return b.String()
}
